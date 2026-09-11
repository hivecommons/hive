# Dashboard REST API reference

Pragmatic v1 endpoint index compiled by hand from route registrations in `src/pkg/dashboard/*.go` and `src/pkg/hub/*.go` (tests excluded). There is no generator; update this page in the same change that adds or renames a route. It lists method, path, coarse auth level, and one-line purpose. Request/response schemas are intentionally not hand-written here; see the handler source for exact payloads and validation.

Auth levels are derived from dashboard middleware (`isPublicPath`, dashboard token/session auth, and the `/api/v1` GitHub-token wrapper) or from hub route wrappers such as `requireAuth` (rows marked `Hub auth`) and `requireAdmin` (rows marked `Hub admin`). Rows marked `Owner only` pass dashboard auth **and** an in-handler `requireOwnerRole` gate (`pkg/dashboard/api.go`): the handler answers `403 owner access required` unless `authenticate` established owner-level access (trusted owner session, proof-verified hub proxy identity, or shared-token operator) — a client-supplied `X-Hive-Role` header is never enough. Rows marked `Merger/owner role` use the analogous `requireMergerOrOwnerRole` gate. Hub rows marked handler-specific have no dashboard middleware; check the named handler for bearer secrets, admin checks, or public behavior.

## Health, status, events

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/version` | Dashboard auth/session | Build/version metadata | `pkg/dashboard/api.go:41` |
| `GET` | `/api/health` | Public | Basic health probe | `pkg/dashboard/server.go:975` |
| `GET` | `/api/health/deep` | Public | Deep health probe | `pkg/dashboard/server.go:976` |
| `GET` | `/api/livez` | Public | Kubernetes liveness probe | `pkg/dashboard/server.go:977` |
| `GET` | `/metrics` | Registered only when `HIVE_METRICS_ENABLED`; requires `Authorization: Bearer $HIVE_METRICS_TOKEN` (403 if the token is unset) | Prometheus metrics | `pkg/dashboard/server.go:983` |
| `GET` | `/api/status` | Dashboard auth/session | Dashboard aggregate status | `pkg/dashboard/server.go:988` |
| `GET` | `/api/events` | Dashboard auth/session | Server-sent event stream | `pkg/dashboard/server.go:989` |

## Snapshots and style

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/style` | Public | Sanitized custom dashboard CSS | `pkg/dashboard/api.go:42` |
| `GET` | `/branding/custom.css` | Dashboard auth/session | Operator branding stylesheet override, read per request (see [branding](branding.md)) | `pkg/dashboard/server.go:1047` |
| `GET` | `/api/snapshot/frame-ancestors` | Public | Snapshot framing allowlist | `pkg/dashboard/api.go:60` |
| `GET` | `/api/snapshot` | Public | Snapshot data | `pkg/dashboard/api.go:61` |
| `GET` | `/snapshot` | Public | Public read-only snapshot page | `pkg/dashboard/api.go:62` |

## Auth and external accounts

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/gh-auth` | Dashboard auth/session | GitHub Auth | `pkg/dashboard/api.go:120` |
| `GET` | `/api/gh-rate-limits` | Dashboard auth/session | GitHub Rate Limits | `pkg/dashboard/api.go:121` |
| `GET` | `/api/gh-user-auth/status` | Public | GitHub User Auth Status | `pkg/dashboard/api.go:122` |
| `POST` | `/api/gh-user-auth/start` | Public | GitHub User Auth Start | `pkg/dashboard/api.go:123` |
| `POST` | `/api/gh-user-auth/poll` | Public | GitHub User Auth Poll | `pkg/dashboard/api.go:124` |
| `POST` | `/api/gh-user-auth/logout` | Public | GitHub User Auth Logout | `pkg/dashboard/api.go:125` |
| `GET` | `/api/gh-user-auth/session` | Public | GitHub User Auth Session | `pkg/dashboard/api.go:126` |
| `GET` | `/api/claude-auth/status` | Dashboard auth/session | Claude Auth Status | `pkg/dashboard/claude_auth.go:54` |
| `POST` | `/api/claude-auth/start` | Owner only | Claude Auth Start | `pkg/dashboard/claude_auth.go:55` |
| `POST` | `/api/claude-auth/exchange` | Owner only | Claude Auth Exchange | `pkg/dashboard/claude_auth.go:56` |
| `POST` | `/api/claude-auth/logout` | Owner only | Claude Auth Logout | `pkg/dashboard/claude_auth.go:57` |
| `GET` | `/api/copilot-auth/status` | Dashboard auth/session | Copilot Auth Status | `pkg/dashboard/copilot_auth.go:84` |
| `POST` | `/api/copilot-auth/start` | Owner only | Copilot Auth Start | `pkg/dashboard/copilot_auth.go:85` |
| `POST` | `/api/copilot-auth/logout` | Owner only | Copilot Auth Logout | `pkg/dashboard/copilot_auth.go:86` |
| `GET` | `/api/openrouter/connect/start` | Dashboard auth/session | Open Router Start | `pkg/dashboard/openrouter.go:66` |
| `GET` | `/api/openrouter/qr` | Dashboard auth/session | Open Router QR | `pkg/dashboard/openrouter.go:67` |
| `GET` | `/api/openrouter/models` | Dashboard auth/session | Open Router Models | `pkg/dashboard/openrouter.go:68` |
| `GET` | `/api/openrouter/credit` | Dashboard auth/session | Open Router Credit | `pkg/dashboard/openrouter.go:69` |
| `GET` | `/openrouter/callback` | Public | Open Router Callback | `pkg/dashboard/openrouter.go:70` |
| `POST` | `/api/github-app/recheck` | Dashboard auth/session | GitHub App Recheck | `pkg/dashboard/server.go:990` |
| `POST` | `/api/github-app/install-clicked` | Dashboard auth/session | GitHub App Install Clicked | `pkg/dashboard/server.go:991` |
| `GET` | `/gh-setup` | Public | GitHub App Setup Callback | `pkg/dashboard/server.go:992` |

## Configuration

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/config` | Dashboard auth/session | Config | `pkg/dashboard/api.go:43` |
| `GET` | `/api/config/download` | Owner only | Config Download | `pkg/dashboard/api.go:44` |
| `GET` | `/api/config/provenance` | Owner only | Config Provenance | `pkg/dashboard/api.go:45` |
| `GET` | `/api/config/variables` | Dashboard auth/session | Variables List | `pkg/dashboard/api.go:46` |
| `GET` | `/api/config/authorized-users` | Dashboard auth/session | Authorized Users List | `pkg/dashboard/api.go:47` |
| `PUT` | `/api/config/variables/{name}` | Owner only | Variable Upsert | `pkg/dashboard/api.go:48` |
| `DELETE` | `/api/config/variables/{name}` | Owner only | Variable Delete | `pkg/dashboard/api.go:49` |
| `GET` | `/api/config/agent/{name}` | Dashboard auth/session | Agent Config Get | `pkg/dashboard/api.go:133` |
| `PUT` | `/api/config/agent/{name}/general` | Owner only | Agent Config General | `pkg/dashboard/api.go:134` |
| `PUT` | `/api/config/agent/{name}/cadences` | Owner only | Agent Config Cadences | `pkg/dashboard/api.go:135` |
| `PUT` | `/api/config/agent/{name}/models` | Owner only | Agent Config Models | `pkg/dashboard/api.go:136` |
| `PUT` | `/api/config/agent/{name}/pipeline` | Owner only | Agent Config Pipeline | `pkg/dashboard/api.go:137` |
| `PUT` | `/api/config/agent/{name}/hooks` | Owner only | Agent Config Hooks | `pkg/dashboard/api.go:138` |
| `PUT` | `/api/config/agent/{name}/restrictions` | Owner only | Agent Config Restrictions | `pkg/dashboard/api.go:139` |
| `PUT` | `/api/config/agent/{name}/stats` | Owner only | Agent Config Stats | `pkg/dashboard/api.go:140` |
| `GET` | `/api/config/agent/{name}/prompt` | Dashboard auth/session | Agent Prompt | `pkg/dashboard/api.go:141` |
| `PUT` | `/api/config/agent/{name}/prompt` | Owner only | Agent Prompt Save | `pkg/dashboard/api.go:142` |
| `GET` | `/api/config/agent/{name}/export` | Dashboard auth/session | Agent Export | `pkg/dashboard/api.go:143` |
| `PUT` | `/api/config/agent/{name}/channels` | Owner only | Agent Config Channels | `pkg/dashboard/api.go:144` |
| `PUT` | `/api/config/agent/{name}/tools` | Owner only | Agent Config Tools | `pkg/dashboard/api.go:145` |
| `PUT` | `/api/config/agent/{name}/connections` | Owner only | Agent Config Connections | `pkg/dashboard/api.go:146` |
| `GET` | `/api/config/stat-sources` | Dashboard auth/session | Stat Sources | `pkg/dashboard/api.go:147` |
| `GET` | `/api/config/governor` | Dashboard auth/session | Governor Config Get | `pkg/dashboard/api.go:149` |
| `PUT` | `/api/config/governor/sensing` | Owner only | Governor Sensing | `pkg/dashboard/api.go:150` |
| `PUT` | `/api/config/governor/thresholds` | Owner only | Governor Thresholds | `pkg/dashboard/api.go:151` |
| `PUT` | `/api/config/governor/labels` | Owner only | Governor Labels | `pkg/dashboard/api.go:154` |
| `PUT` | `/api/config/governor/budget` | Owner only | Governor Budget | `pkg/dashboard/api.go:155` |
| `PUT` | `/api/config/governor/notifications` | Owner only | Governor Notifications | `pkg/dashboard/api.go:157` |
| `PUT` | `/api/config/governor/health` | Owner only | Governor Health | `pkg/dashboard/api.go:158` |
| `PUT` | `/api/config/governor/logging` | Owner only | Governor Logging | `pkg/dashboard/api.go:168` |
| `PUT` | `/api/config/governor/attribution` | Owner only | Governor Attribution | `pkg/dashboard/api.go:169` |
| `PUT` | `/api/config/governor/hub` | Owner only | Governor Hub | `pkg/dashboard/api.go:170` |
| `PUT` | `/api/config/governor/litellm` | Owner only | Governor Lite LLM | `pkg/dashboard/api.go:171` |
| `PUT` | `/api/config/governor/trajectory` | Owner only | Governor Trajectory | `pkg/dashboard/api.go:172` |
| `GET` | `/api/config/governor/backup` | Owner only | Backup Key Status (presence + safe source label; never the key value) | `pkg/dashboard/backup_key.go` |
| `PUT` | `/api/config/governor/backup` | Owner only | Backup Key Set (64-hex AES-256 key; stored 0600, path-only in `hive.yaml`) | `pkg/dashboard/backup_key.go` |
| `DELETE` | `/api/config/governor/backup` | Owner only | Backup Key Clear (backups are refused again) | `pkg/dashboard/backup_key.go` |
| `GET` | `/api/config/governor/bob` | Dashboard auth/session | Governor Bob Status | `pkg/dashboard/api.go:205` |
| `PUT` | `/api/config/governor/bob` | Owner only | Governor Bob Key | `pkg/dashboard/api.go:206` |
| `DELETE` | `/api/config/governor/bob` | Owner only | Governor Bob Key Clear | `pkg/dashboard/api.go:207` |
| `POST` | `/api/config/governor/bob/test` | Dashboard auth/session | Governor Bob Key Test | `pkg/dashboard/api.go:209` |
| `POST` | `/api/config/governor/litellm/test` | Dashboard auth/session | Governor Lite LLMTest | `pkg/dashboard/api.go:210` |
| `GET` | `/api/config/governor/gateways` | Dashboard auth/session | Governor Gateways List | `pkg/dashboard/api.go:213` |
| `PUT` | `/api/config/governor/gateways` | Owner only | Governor Gateways Upsert | `pkg/dashboard/api.go:214` |
| `DELETE` | `/api/config/governor/gateways/{name}` | Owner only | Governor Gateways Delete | `pkg/dashboard/api.go:215` |
| `POST` | `/api/config/governor/gateways/{name}/test` | Dashboard auth/session | Governor Gateways Test | `pkg/dashboard/api.go:216` |
| `POST` | `/api/config/governor/gateways/discover` | Owner only | Governor Gateways Discover | `pkg/dashboard/api.go:217` |
| `POST` | `/api/config/governor/agents` | Owner only | Governor Add Agent | `pkg/dashboard/api.go:220` |
| `DELETE` | `/api/config/governor/agents/{name}` | Owner only | Governor Remove Agent | `pkg/dashboard/api.go:221` |
| `PUT` | `/api/config/governor/repos` | Owner only | Governor Repos | `pkg/dashboard/api.go:222` |
| `POST` | `/api/config/governor/repos/check-access` | Dashboard auth/session | Governor Repo Check Access | `pkg/dashboard/api.go:226` |
| `PUT` | `/api/config/github` | Owner only | Config GitHub | `pkg/dashboard/api.go:227` |
| `GET` | `/api/config/github/forge-apps` | Dashboard auth/session | Config GitHub Forge Apps | `pkg/dashboard/api.go:230` |
| `GET` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Get | `pkg/dashboard/api.go:250` |
| `PUT` | `/api/config/sidebar` | Dashboard auth/session | Sidebar Set | `pkg/dashboard/api.go:251` |
| `GET` | `/api/config/backends` | Dashboard auth/session | Backends | `pkg/dashboard/api.go:252` |
| `GET` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Get | `pkg/dashboard/api.go:152` |
| `PUT` | `/api/config/governor/threshold-scaling` | Owner only | Governor Threshold Scaling Set | `pkg/dashboard/api.go:153` |
| `POST` | `/api/config/governor/budget/reset` | Owner only | Governor Budget Reset | `pkg/dashboard/api.go:156` |
| `PUT` | `/api/config/governor/watchdog` | Owner only | Governor Watchdog | `pkg/dashboard/api.go:159` |
| `GET` | `/api/config/escalation` | Owner only | Escalation Config Get | `pkg/dashboard/api.go:162` |
| `PUT` | `/api/config/escalation` | Owner only | Escalation Config Set | `pkg/dashboard/api.go:163` |
| `GET` | `/api/config/review` | Dashboard auth/session | Review Config Get | `pkg/dashboard/api.go:166` |
| `PUT` | `/api/config/review` | Owner only | Review Config Set | `pkg/dashboard/api.go:167` |
| `PUT` | `/api/config/governor/features` | Owner only | Governor Features | `pkg/dashboard/api.go:173` |
| `GET` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Get | `pkg/dashboard/api.go:174` |
| `PUT` | `/api/config/governor/general-advanced` | Owner only | Governor General Advanced Set | `pkg/dashboard/api.go:175` |
| `GET` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Get | `pkg/dashboard/api.go:179` |
| `PUT` | `/api/config/auto-merge` | Owner only | Auto-Merge Config Set | `pkg/dashboard/api.go:180` |
| `GET` | `/api/config/convergence` | Owner only | Convergence Config Get | `pkg/dashboard/api.go:185` |
| `PUT` | `/api/config/convergence` | Owner only | Convergence Config Set | `pkg/dashboard/api.go:186` |
| `GET` | `/api/config/governor/advisory` | Owner only | Governor Advisory Get | `pkg/dashboard/api.go:188` |
| `PUT` | `/api/config/governor/advisory` | Owner only | Governor Advisory Set | `pkg/dashboard/api.go:189` |
| `GET` | `/api/config/governor/replan` | Owner only | Governor Replan Get | `pkg/dashboard/api.go:190` |
| `PUT` | `/api/config/governor/replan` | Owner only | Governor Replan Set | `pkg/dashboard/api.go:191` |
| `GET` | `/api/config/governor/work-source` | Owner only | Governor Work Source Get | `pkg/dashboard/api.go:192` |
| `PUT` | `/api/config/governor/work-source` | Owner only | Governor Work Source Set | `pkg/dashboard/api.go:193` |
| `PUT` | `/api/config/governor/security` | Owner only | Governor Security | `pkg/dashboard/api.go:194` |
| `GET` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Get | `pkg/dashboard/api.go:195` |
| `PUT` | `/api/config/governor/project-observability` | Owner only | Governor Project Observability Set | `pkg/dashboard/api.go:196` |
| `GET` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Get | `pkg/dashboard/api.go:211` |
| `PUT` | `/api/config/governor/inference-auth` | Owner only | Governor Inference Auth Set | `pkg/dashboard/api.go:212` |

## Agents and controls

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/kick/{agent}` | Owner only | Kick — asynchronous; answers `202` once queued (see below) | `pkg/dashboard/api.go:84` |
| `GET` | `/api/kick/{agent}/status` | Dashboard auth/session | Outcome of the most recent kick | `pkg/dashboard/api.go:88` |
| `POST` | `/api/switch/{agent}/{backend}` | Owner only | Switch | `pkg/dashboard/api.go:89` |
| `POST` | `/api/model/{agent}/{model}` | Owner only | Model Set | `pkg/dashboard/api.go:90` |
| `POST` | `/api/effort/{agent}/{effort}` | Owner only | Set launch-only reasoning effort after validating against the agent's live backend (including runtime override); persists config/agent overlay, restarts the session, and `default` clears the stored effort | `pkg/dashboard/api.go:91` |
| `POST` | `/api/pause/{agent}` | Owner only | Pause | `pkg/dashboard/api.go:92` |
| `POST` | `/api/resume/{agent}` | Owner only | Resume | `pkg/dashboard/api.go:93` |
| `GET` | `/api/agent-state/{agent}` | Dashboard auth/session | Agent State | `pkg/dashboard/api.go:94` |
| `GET` | `/api/breaker` | Dashboard auth/session | Breaker State | `pkg/dashboard/api.go:95` |
| `POST` | `/api/breaker/engage` | Owner only | Breaker Engage | `pkg/dashboard/api.go:96` |
| `POST` | `/api/breaker/release` | Owner only | Breaker Release | `pkg/dashboard/api.go:97` |
| `POST` | `/api/pin/{agent}/{dimension}` | Owner only | Pin | `pkg/dashboard/api.go:98` |
| `POST` | `/api/unpin/{agent}/{dimension}` | Owner only | Unpin | `pkg/dashboard/api.go:99` |
| `POST` | `/api/restart/{agent}` | Owner only | Restart | `pkg/dashboard/api.go:103` |
| `GET` | `/api/model-advisor` | Dashboard auth/session | Model Advisor | `pkg/dashboard/api.go:116` |
| `GET` | `/api/agents` | Dashboard auth/session | Agents List | `pkg/dashboard/api.go:232` |
| `POST` | `/api/agents` | Owner only | Agent Create | `pkg/dashboard/api.go:233` |
| `POST` | `/api/agents/import` | Owner only | Agent Import | `pkg/dashboard/api.go:234` |
| `DELETE` | `/api/agents/{name}` | Owner only | Agent Delete | `pkg/dashboard/api.go:235` |
| `GET` | `/api/agents/{name}/log` | Dashboard auth/session | Agent Full Log | `pkg/dashboard/api.go:71` |
| `GET` | `/api/agents/{name}/terminal-urls` | Dashboard auth/session | Agent Terminal URLs | `pkg/dashboard/api.go:75` |
| `GET` | `/api/agents/{name}/kicks` | Dashboard auth/session | Agent Kick Log List | `pkg/dashboard/api.go:78` |
| `GET` | `/api/agents/{name}/kicks/{id}` | Dashboard auth/session | Agent Kick Log Get | `pkg/dashboard/api.go:79` |
| `GET` | `/agents/{name}/kicks` | Dashboard auth/session | Agent Kick History Page (HTML) | `pkg/dashboard/api.go:80` |
| `POST` | `/api/agents/{name}/login-code` | Owner only | Agent Login Code | `pkg/dashboard/api.go:102` |

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
| `GET` | `/api/packs` | Dashboard auth/session | Packs List | `pkg/dashboard/api.go:237` |
| `POST` | `/api/packs/{level}/apply` | Owner only | Pack Apply | `pkg/dashboard/api.go:238` |
| `PUT` | `/api/packs/level` | Owner only | Pack Set Level | `pkg/dashboard/api.go:239` |
| `POST` | `/api/repos/rescan` | Dashboard auth/session | Re-enumerate watched repositories' open issues/PRs for the dashboard Repositories cards. The scan is read-only with respect to agents/governor actions, collapses concurrent requests, and debounces repeated presses for 30 seconds (`repoRescanDebounce`) so the button cannot hammer GitHub | `pkg/dashboard/api.go`, `pkg/dashboard/api_repos_rescan.go` |
| `GET` | `/api/acmm/evaluation` | Dashboard auth/session | ACMMEvaluation — combined codebase + operational result, cached server-side for 1 hour (`acmmEvalTTL`). `?refresh=1` (the dashboard's "🔄 Re-evaluate" button, #5877) bypasses the hourly TTL but is debounced server-side: requests within 1 minute of the last evaluation (`acmmRefreshDebounce`) still serve the cache, since a full refresh costs up to ~29 GitHub GetContents calls per repo. The response's `last_evaluated_at` timestamp reports when the cached evaluation was computed | `pkg/dashboard/api.go:246`, `pkg/dashboard/api_acmm_eval.go` |
| `POST` | `/api/acmm/issue` | Owner only | ACMMCreate Issue — files on GitHub or, with `governor.acmm.issue_tracker: work_source` / body `tracker: "work_source"` on a Linear-sourced hive, on Linear; response `tracker` says which. See [ACMM policy matrix](acmm-policy-matrix.md#where-acmm-gap-issues-are-filed) | `pkg/dashboard/api.go:247` |
| `GET` | `/api/acmm-recommendation` | Dashboard auth/session | Advisory level-up recommendation (`acmmadvisor.Recommendation`, JSON): never changes the applied level — see [ACMM advisor](acmm-advisor.md) | `pkg/dashboard/api.go:248` |

## Cost, tokens, telemetry

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/trends` | Dashboard auth/session | Trends | `pkg/dashboard/api.go:64` |
| `GET` | `/api/token-access` | Owner only | Token Access | `pkg/dashboard/api.go:106` |
| `GET` | `/api/tokens` | Dashboard auth/session | Tokens | `pkg/dashboard/api.go:107` |
| `GET` | `/api/cost` | Dashboard auth/session | Cost | `pkg/dashboard/api.go:108` |
| `GET` | `/api/repo-activity` | Dashboard auth/session | Per-repo audited activity | `pkg/dashboard/api.go` |
| `GET` | `/api/repo-cost` | Dashboard auth/session | Per-repo estimated token cost (interval join, cached) | `pkg/dashboard/api.go` |
| `GET` | `/api/cost/history` | Dashboard auth/session | Cost History | `pkg/dashboard/api.go:111` |
| `GET` | `/api/trend/history` | Dashboard auth/session | Trend History | `pkg/dashboard/api.go:114` |
| `GET` | `/api/timeseries` | Dashboard auth/session | Time Series | `pkg/dashboard/api.go:115` |
| `GET` | `/api/providers/headroom` | Owner only | Providers Headroom | `pkg/dashboard/api.go:51` |
| `GET` | `/api/budget/history` | Dashboard auth/session | Budget History | `pkg/dashboard/api.go:113` |

## Knowledge

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/knowledge` | Dashboard auth/session | Knowledge List | `pkg/dashboard/api.go:255` |
| `GET` | `/api/knowledge/export` | Dashboard auth/session | Knowledge Export | `pkg/dashboard/api.go:256` |
| `GET` | `/api/knowledge/search` | Dashboard auth/session | Knowledge Search | `pkg/dashboard/api.go:257` |
| `GET` | `/api/knowledge/health` | Dashboard auth/session | Knowledge Health | `pkg/dashboard/api.go:258` |
| `GET` | `/api/knowledge/stats` | Dashboard auth/session | Knowledge Stats | `pkg/dashboard/api.go:259` |
| `GET` | `/api/knowledge/graph` | Dashboard auth/session | Knowledge Graph | `pkg/dashboard/api.go:260` |
| `GET` | `/api/knowledge/fact-history` | Dashboard auth/session | Fact History | `pkg/dashboard/api.go:261` |
| `POST` | `/api/knowledge/create` | Dashboard auth/session | Knowledge Create | `pkg/dashboard/api.go:262` |
| `POST` | `/api/knowledge/import` | Dashboard auth/session | Knowledge Import | `pkg/dashboard/api.go:263` |
| `POST` | `/api/knowledge/promote` | Dashboard auth/session | Knowledge Promote | `pkg/dashboard/api.go:269` |
| `GET` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs List | `pkg/dashboard/api.go:270` |
| `POST` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Add | `pkg/dashboard/api.go:271` |
| `DELETE` | `/api/knowledge/subscriptions` | Dashboard auth/session | Knowledge Subs Remove | `pkg/dashboard/api.go:272` |
| `PUT` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Update | `pkg/dashboard/api.go:273` |
| `DELETE` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Delete | `pkg/dashboard/api.go:274` |
| `GET` | `/api/knowledge/{layer}` | Dashboard auth/session | Knowledge Layer | `pkg/dashboard/api.go:275` |
| `GET` | `/api/knowledge/{layer}/{slug}` | Dashboard auth/session | Knowledge Fact | `pkg/dashboard/api.go:276` |
| `PUT` | `/api/knowledge/enabled` | Owner only | Knowledge Toggle | `pkg/dashboard/api.go:277` |
| `GET` | `/api/knowledge/bead-synthesizer` | Dashboard auth/session | Bead Synth Status | `pkg/dashboard/api.go:278` |
| `PUT` | `/api/knowledge/bead-synthesizer/enabled` | Owner only | Bead Synth Toggle | `pkg/dashboard/api.go:279` |
| `GET` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults List | `pkg/dashboard/api.go:280` |
| `POST` | `/api/knowledge/vaults` | Owner only | Vaults Connect | `pkg/dashboard/api.go:281` |
| `DELETE` | `/api/knowledge/vaults` | Dashboard auth/session | Vaults Disconnect | `pkg/dashboard/api.go:282` |
| `POST` | `/api/knowledge/vaults/reindex` | Dashboard auth/session | Vaults Reindex | `pkg/dashboard/api.go:283` |
| `GET` | `/api/knowledge/vaults/{name}/facts` | Dashboard auth/session | Vault Facts | `pkg/dashboard/api.go:284` |
| `GET` | `/api/knowledge/git-sources` | Dashboard auth/session | Git Sources List | `pkg/dashboard/api.go:285` |
| `POST` | `/api/knowledge/git-sources` | Owner only | Git Sources Connect | `pkg/dashboard/api.go:286` |
| `DELETE` | `/api/knowledge/git-sources` | Owner only | Git Sources Disconnect | `pkg/dashboard/api.go:287` |
| `POST` | `/api/knowledge/obsidian/sync` | Dashboard auth/session | Obsidian Sync | `pkg/dashboard/api.go:288` |
| `GET` | `/api/knowledge/documents` | Dashboard auth/session | Documents List | `pkg/dashboard/api.go:289` |
| `POST` | `/api/knowledge/documents` | Dashboard auth/session | Documents Import | `pkg/dashboard/api.go:290` |
| `GET` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Get | `pkg/dashboard/api.go:291` |
| `DELETE` | `/api/knowledge/documents/{slug}` | Dashboard auth/session | Document Delete | `pkg/dashboard/api.go:292` |
| `POST` | `/api/knowledge/documents/{slug}/reimport` | Dashboard auth/session | Document Reimport | `pkg/dashboard/api.go:293` |
| `GET` | `/api/knowledge/context7/search` | Dashboard auth/session | Context7 Search | `pkg/dashboard/api.go:294` |
| `POST` | `/api/knowledge/cleanup-orphans` | Dashboard auth/session | Cleanup Orphans | `pkg/dashboard/api.go:295` |
| `GET` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channels List | `pkg/dashboard/api.go:267` |
| `POST` | `/api/knowledge/channels` | Dashboard auth/session | Knowledge Channel Create | `pkg/dashboard/api.go:268` |

## Contribute

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/contribute` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:291` |
| `GET` | `/contribute/{tab}` | Public | Contribute Landing | `pkg/dashboard/api_contribute.go:297` |
| `GET` | `/api/contribute/ws` | Public | s.contribute Hub.Handle WS | `pkg/dashboard/api_contribute.go:305` |
| `POST` | `/api/contribute/register` | Public | Contribute Register | `pkg/dashboard/api_contribute.go:306` |
| `POST` | `/api/contribute/invite` | Public | Contribute Invite | `pkg/dashboard/api_contribute.go:312` |
| `POST` | `/api/contribute/reissue-token` | Public | Contribute Reissue Token | `pkg/dashboard/api_contribute.go:313` |
| `GET` | `/api/contribute/status` | Public | Contribute Status | `pkg/dashboard/api_contribute.go:314` |
| `GET` | `/api/contribute/activity` | Public | Contribute Activity | `pkg/dashboard/api_contribute.go:315` |
| `GET` | `/api/contribute/fleet` | Public | Contribute Fleet | `pkg/dashboard/api_contribute.go:316` |
| `GET` | `/api/contribute/events` | Public | Contribute Events (SSE). Every frame carries a monotonic `seq`; a connection whose channel filled gets a `gap` frame naming how many events it missed, so a long-lived client can tell "nothing happened" from "I missed events" ([#6218](https://github.com/hivecommons/hive/issues/6218)) | `pkg/dashboard/api_contribute.go:320` |
| `GET` | `/api/contribute/queue` | Public | Contribute Queue | `pkg/dashboard/api_contribute.go:323` |
| `GET` | `/api/contribute/opportunistic` | Public | Contribute Opportunistic | `pkg/dashboard/api_contribute.go:327` |
| `GET` | `/api/contribute/limits` | Public | Contribute Limits | `pkg/dashboard/api_contribute.go:332` |
| `GET` | `/api/contribute/metrics` | Public | Contribute Metrics | `pkg/dashboard/api_contribute.go:338` |
| `GET` | `/api/contribute/me` | Public path; caller identity resolved server-side (401 anonymous, 403 without a profile) | The caller's OWN contribution stats — issues worked (24h and all-time), completions that produced a verified PR, and failures. No username parameter, so it only ever answers for its caller ([#6543](https://github.com/hivecommons/hive/issues/6543)) | `pkg/dashboard/api_contribute.go:344` |
| `GET` | `/api/contribute/triage` | Public | Contribute Triage | `pkg/dashboard/api_contribute.go:356` |
| `PUT` | `/api/contribute/queue/order` | Public | Contribute Queue Order | `pkg/dashboard/api_contribute.go:360` |
| `POST` | `/api/contribute/queue/hold` | Public | Contribute Queue Hold | `pkg/dashboard/api_contribute.go:365` |
| `POST` | `/api/contribute/queue/hold/clear` | Public | Contribute Queue Hold Clear | `pkg/dashboard/api_contribute.go:369` |
| `GET` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:375` |
| `PUT` | `/api/contribute/interests` | Public | Contribute Interests | `pkg/dashboard/api_contribute.go:376` |
| `GET` | `/api/contributors` | Dashboard auth/session | Contributors List | `pkg/dashboard/api_contribute.go:382` |
| `GET` | `/api/contributors/{id}` | Dashboard auth/session | Contributor Get | `pkg/dashboard/api_contribute.go:383` |
| `PUT` | `/api/contributors/{id}/trust` | Owner only | Contributor Trust | `pkg/dashboard/api_contribute.go:384` |
| `PUT` | `/api/contributors/{id}/agent-role` | Owner only | Contributor Agent Role | `pkg/dashboard/api_contribute.go:385` |
| `PUT` | `/api/contributors/{id}/agent-role-grants` | Owner only | Contributor Agent Role Grants | `pkg/dashboard/api_contribute.go:386` |
| `POST` | `/api/contributors/{id}/revoke` | Owner only | Contributor Revoke | `pkg/dashboard/api_contribute.go:387` |
| `POST` | `/api/contributors/{id}/requeue` | Dashboard auth/session | Contributor Requeue | `pkg/dashboard/api_contribute.go:388` |
| `DELETE` | `/api/contributors/{id}` | Owner only | Contributor Delete | `pkg/dashboard/api_contribute.go:389` |
| `GET` | `/contribute/dossier/{username}` | Public | Contributor Dossier Page (HTML) | `pkg/dashboard/api_contribute.go:304` |
| `GET` | `/api/contribute/run-stats` | Public | Contribute Run Stats | `pkg/dashboard/api_contribute.go:349` |
| `GET` | `/api/contribute/dossier` | Public | Contribute Dossier Get | `pkg/dashboard/api_contribute.go:380` |
| `POST` | `/api/contribute/dossier` | Public | Contribute Dossier Update | `pkg/dashboard/api_contribute.go:381` |
| `GET` | `/api/leaderboard/contributor/{username}/heraldry` | Public | Contributor Heraldry | `pkg/dashboard/api_contribute.go:408` |

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
| `GET`/`POST` | `/api/v1/status` | GitHub token + allowlist | Contributor status summary | `pkg/dashboard/api_contribute.go:6777` |
| `GET`/`POST` | `/api/v1/queue` | GitHub token + allowlist | Paginated ready-work listing (`?limit=<int>&offset=<int>`) over the actionable/offerable backlog | `pkg/dashboard/api_contribute.go:6604` |
| `GET`/`POST` | `/api/v1/activity` | GitHub token + allowlist | Contributor activity feed | `pkg/dashboard/api_contribute.go:6779` |
| `GET`/`POST` | `/api/v1/contributors` | GitHub token + allowlist | Contributor list | `pkg/dashboard/api_contribute.go:6781` |
| `GET`/`POST` | `/api/v1/knowledge` | GitHub token + allowlist | Knowledge export | `pkg/dashboard/api_contribute.go:6783` |
| `GET`/`POST` | `/api/v1/me` | GitHub token (self-scoped, no allowlist) | Current contributor profile | `pkg/dashboard/api_contribute.go:6785` |
| `POST` | `/api/v1/prs/{owner}/{repo}/{number}/queue-automerge` | GitHub token + merger/owner role (POST only; GET returns 405) | Queue PR auto-merge using the validated actor and current PR head | `pkg/dashboard/api_contribute.go` |

## Nous / strategy lab

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/nous/status` | Dashboard auth/session | Nous Status | `pkg/dashboard/api.go:326` |
| `GET` | `/api/nous/ledger` | Dashboard auth/session | Nous Ledger | `pkg/dashboard/api.go:327` |
| `GET` | `/api/nous/principles` | Dashboard auth/session | Nous Principles | `pkg/dashboard/api.go:328` |
| `POST` | `/api/nous/approve` | Owner only | Nous Approve | `pkg/dashboard/api.go:329` |
| `POST` | `/api/nous/abort` | Owner only | Nous Abort | `pkg/dashboard/api.go:330` |
| `PUT` | `/api/nous/mode` | Owner only | Nous Mode | `pkg/dashboard/api.go:331` |
| `PUT` | `/api/nous/scope` | Owner only | Nous Scope | `pkg/dashboard/api.go:332` |
| `GET` | `/api/nous/phase` | Dashboard auth/session | Nous Phase | `pkg/dashboard/api.go:333` |
| `PUT` | `/api/nous/gate-decision` | Owner only | Nous Gate Decision | `pkg/dashboard/api.go:334` |
| `GET` | `/api/nous/gate-pending` | Dashboard auth/session | Nous Gate Pending | `pkg/dashboard/api.go:335` |
| `POST` | `/api/nous/gate-respond` | Owner only | Nous Gate Respond | `pkg/dashboard/api.go:336` |
| `GET` | `/api/nous/gate-response` | Dashboard auth/session | Nous Gate Response | `pkg/dashboard/api.go:337` |
| `GET` | `/api/nous/config` | Dashboard auth/session | Nous Config Get | `pkg/dashboard/api.go:338` |
| `PUT` | `/api/nous/config/goals` | Owner only | Nous Config Goals | `pkg/dashboard/api.go:339` |
| `PUT` | `/api/nous/config/repos` | Owner only | Nous Config Repos | `pkg/dashboard/api.go:340` |
| `PUT` | `/api/nous/config/output` | Owner only | Nous Config Output | `pkg/dashboard/api.go:341` |
| `PUT` | `/api/nous/config/fast-fail` | Owner only | Nous Config Fast Fail | `pkg/dashboard/api.go:342` |
| `PUT` | `/api/nous/config/schedule` | Owner only | Nous Config Schedule | `pkg/dashboard/api.go:343` |
| `PUT` | `/api/nous/config/controllables` | Owner only | Nous Config Controllables | `pkg/dashboard/api.go:344` |
| `PUT` | `/api/nous/config/principles` | Owner only | Nous Config Principles | `pkg/dashboard/api.go:345` |
| `DELETE` | `/api/nous/principles/{id}` | Owner only | Nous Delete Principle | `pkg/dashboard/api.go:346` |

## Inception

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `POST` | `/api/inception/start` | Owner only | Inception Start | `pkg/dashboard/api.go:300` |
| `POST` | `/api/inception/scan` | Owner only | Inception Scan | `pkg/dashboard/api.go:301` |
| `GET` | `/api/inception/state` | Dashboard auth/session | Inception State | `pkg/dashboard/api.go:302` |
| `POST` | `/api/inception/questions` | Owner only | Inception Set Questions | `pkg/dashboard/api.go:303` |
| `POST` | `/api/inception/answer` | Owner only | Inception Answer | `pkg/dashboard/api.go:304` |
| `POST` | `/api/inception/facts` | Owner only | Inception Record Facts | `pkg/dashboard/api.go:305` |
| `GET` | `/api/inception/scaffold` | Dashboard auth/session | Inception Scaffold | `pkg/dashboard/api.go:306` |
| `POST` | `/api/inception/approve` | Owner only | Inception Approve | `pkg/dashboard/api.go:307` |
| `POST` | `/api/inception/reset` | Owner only | Inception Reset | `pkg/dashboard/api.go:308` |
| `GET` | `/api/inception/ideation-facts` | Dashboard auth/session | Inception Ideation Facts | `pkg/dashboard/api.go:309` |
| `GET` | `/api/inception/download` | Dashboard auth/session | Inception Download | `pkg/dashboard/api.go:310` |
| `GET` | `/api/inception/has-files` | Dashboard auth/session | Inception Has Files | `pkg/dashboard/api.go:311` |
| `PUT` | `/api/inception/wiki-name` | Owner only | Inception Rename Wiki | `pkg/dashboard/api.go:312` |
| `POST` | `/api/inception/import` | Owner only | Inception Import | `pkg/dashboard/api.go:313` |

## Beads

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/beads` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:348` |
| `GET` | `/api/beads/{agent}` | Dashboard auth/session | Beads List | `pkg/dashboard/api.go:349` |
| `POST` | `/api/beads/{agent}` | Owner only | Beads Create | `pkg/dashboard/api.go:350` |
| `POST` | `/api/beads/reset` | Owner only | Beads Reset | `pkg/dashboard/api.go:351` |
| `POST` | `/api/beads/reset/{agent}` | Owner only | Beads Reset Agent | `pkg/dashboard/api.go:352` |

## Dashboard miscellaneous

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/audit` | Read-write role | Audit Log — `{"entries": [...]}` envelope, newest first, capped at 200; response shape and the serve-time `user_name` field in [audit-log.md](audit-log.md#get-apiaudit) | `pkg/dashboard/api.go:50` |
| `POST` | `/api/presence` | Dashboard auth/session | Presence | `pkg/dashboard/api.go:52` |
| `GET` | `/api/prompt-history` | Dashboard auth/session | Prompt History | `pkg/dashboard/api.go:53` |
| `POST` | `/api/self-upgrade` | Owner only | Self Upgrade | `pkg/dashboard/api.go:54` |
| `GET` | `/api/backup/status` | Owner only | Backup Status — `available:false` with a reason when no encryption key is configured | `pkg/dashboard/api.go:57` |
| `POST` | `/api/backup` | Owner only | Backup Download — `412` when no encryption key is configured (never an unencrypted archive) | `pkg/dashboard/api.go:58` |
| `POST` | `/api/banner-dismissed` | Dashboard auth/session | Banner Dismissed | `pkg/dashboard/api.go:59` |
| `GET` | `/api/history` | Dashboard auth/session | History | `pkg/dashboard/api.go:63` |
| `GET` | `/api/timeline` | Dashboard auth/session | Timeline | `pkg/dashboard/api.go:65` |
| `GET` | `/api/lifecycle-timeline` | Dashboard auth/session | Issue→PR lifecycle journeys plus derived stage timeline | `pkg/dashboard/api.go:66` |
| `GET` | `/api/convergence/soak` | Owner only | Convergence Soak Status | `pkg/dashboard/api.go:187` |
| `POST` | `/api/plan/from-issue` | Dashboard auth/session | Plan From Issue | `pkg/dashboard/api.go:318` |
| `GET` | `/api/plan/{epicID}` | Dashboard auth/session | Plan Tree | `pkg/dashboard/api.go:319` |
| `POST` | `/api/plan/{epicID}/approve` | Owner only | Plan Approve | `pkg/dashboard/api.go:320` |
| `POST` | `/api/plan/{epicID}/reject` | Owner only | Plan Reject | `pkg/dashboard/api.go:321` |
| `POST` | `/api/plan/{epicID}/child/{childID}` | Owner only | Plan Child Action | `pkg/dashboard/api.go:322` |
| `POST` | `/api/linear/agent/install` | Owner only | Linear Agent Install (OAuth start) | `pkg/dashboard/api_linear_agent.go:271` |
| `GET` | `/api/linear/agent/status` | Owner only | Linear Agent Status | `pkg/dashboard/api_linear_agent.go:272` |
| `POST` | `/api/linear/agent/disconnect` | Owner only | Linear Agent Disconnect | `pkg/dashboard/api_linear_agent.go:273` |
| `GET` | `/api/widget` | Dashboard auth/session | Widget | `pkg/dashboard/api.go:67` |
| `GET` | `/api/pane/{agent}` | Dashboard auth/session | Pane | `pkg/dashboard/api.go:68` |
| `GET` | `/api/role` | Dashboard auth/session | Role | `pkg/dashboard/api.go:82` |
| `POST` | `/api/reset-restarts/{agent}` | Owner only | Reset Restarts | `pkg/dashboard/api.go:104` |
| `GET` | `/api/budget-ignore` | Dashboard auth/session | Budget Ignore Get | `pkg/dashboard/api.go:117` |
| `POST` | `/api/budget-ignore` | Owner only | Budget Ignore Set | `pkg/dashboard/api.go:118` |
| `GET` | `/api/summaries` | Dashboard auth/session | Summaries | `pkg/dashboard/api.go:130` |
| `POST` | `/api/prs/{owner}/{repo}/{number}/queue-automerge` | Merger/owner role | Queue PRAuto Merge | `pkg/dashboard/api.go:131` |
| `GET` | `/api/inference/models/{backend}` | Dashboard auth/session | Inference Models | `pkg/dashboard/api.go:253` |
| `GET` | `/api/hive-id` | Dashboard auth/session | Hive IDGet | `pkg/dashboard/api.go:297` |
| `PUT` | `/api/hive-id` | Owner only | Hive IDSet | `pkg/dashboard/api.go:298` |
| `POST` | `/api/chat` | Dashboard auth/session | Chat | `pkg/dashboard/api.go:324` |
| `GET` | `/api/auth/token` | Public | Auth Token | `pkg/dashboard/api.go:354` |
| `GET` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:391` |
| `POST` | `/api/v1/` | GitHub token | Contributor v1 API dispatcher | `pkg/dashboard/api_contribute.go:392` |
| `GET` | `/api/docs` | Dashboard auth/session | APIDocs | `pkg/dashboard/api_contribute.go:393` |
| `GET` | `/leaderboard` | Public | Leaderboard Page | `pkg/dashboard/api_contribute.go:395` |
| `GET` | `/api/leaderboard` | Public | Leaderboard API | `pkg/dashboard/api_contribute.go:396` |
| `GET` | `/api/leaderboard/style` | Public | Leaderboard Style | `pkg/dashboard/api_contribute.go:397` |
| `GET` | `/api/leaderboard/contributor/{username}` | Public | Contributor Profile | `pkg/dashboard/api_contribute.go:403` |
| `GET` | `/api/hives` | Dashboard auth/session | Hives List | `pkg/dashboard/api_contribute.go:410` |
| `POST` | `/api/hives/register` | Dashboard auth/session | Hives Register | `pkg/dashboard/api_contribute.go:411` |
| `POST` | `/api/hives/{id}/heartbeat` | Dashboard auth/session | Hives Heartbeat | `pkg/dashboard/api_contribute.go:412` |
| `DELETE` | `/api/hives/{id}` | Owner only | Hives Delete | `pkg/dashboard/api_contribute.go:413` |
| `POST` | `/api/hives/onboard` | Dashboard auth/session | Hives Onboard | `pkg/dashboard/api_contribute.go:414` |
| `GET` | `/sso` | Public | SSO | `pkg/dashboard/server.go:997` |

## Hub SaaS

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/api/saas/my-hives` | Hub auth | My Hives | `pkg/hub/saas.go:551` |
| `GET` | `/api/saas/usage` | Hub auth | Usage | `pkg/hub/saas.go:560` |
| `POST` | `/api/saas/hives` | Hub auth | Create Hive | `pkg/hub/saas.go:573` |
| `GET` | `/api/saas/hives/{id}/status` | Hub auth | Hive Status | `pkg/hub/saas.go:574` |
| `GET` | `/api/saas/hives/{id}/open` | Hub handler-specific | Open Hive | `pkg/hub/saas.go:579` |
| `DELETE` | `/api/saas/hives/{id}` | Hub auth | Delete Hive | `pkg/hub/saas.go:580` |
| `POST` | `/api/saas/hives/{id}/upgrade` | Hub auth | Upgrade Hive | `pkg/hub/saas.go:581` |
| `POST` | `/api/saas/hives/{id}/switch-branch` | Hub auth | Switch Branch | `pkg/hub/saas.go:582` |
| `PUT` | `/api/saas/hives/{id}/visibility` | Hub auth | Toggle Visibility | `pkg/hub/saas.go:583` |
| `PUT` | `/api/saas/hives/{id}/auto-upgrade` | Hub auth | Toggle Auto Upgrade | `pkg/hub/saas.go:584` |
| `PUT` | `/api/saas/hives/{id}/name` | Hub auth | Rename Hive | `pkg/hub/saas.go:588` |
| `POST` | `/api/saas/hives/{id}/forge` | Hub auth | Switch Forge | `pkg/hub/saas.go:592` |
| `POST` | `/api/saas/hives/{id}/reset-app` | Hub auth | Reset App | `pkg/hub/saas.go:593` |
| `POST` | `/api/saas/hives/{id}/restart-spoke` | Hub auth | Restart Spoke | `pkg/hub/saas.go:598` |
| `GET` | `/api/saas/hive-config/{hiveID}` | Hub auth | Proxy Hive Config | `pkg/hub/saas.go:600` |
| `GET` | `/api/saas/latest-sha` | Hub handler-specific | Latest SHA | `pkg/hub/saas.go:601` |
| `POST` | `/api/saas/hub/upgrade` | Hub handler-specific | Hub Self Upgrade | `pkg/hub/saas.go:602` |
| `PUT` | `/api/saas/hub/auto-upgrade` | Hub handler-specific | Hub Auto Upgrade | `pkg/hub/saas.go:603` |
| `GET` | `/api/saas/auth-check` | Hub handler-specific | Saa SAuth Check | `pkg/hub/saas.go:608` |
| `POST` | `/api/saas/user-token` | Hub auth | User Token | `pkg/hub/saas.go:619` |
| `GET` | `/api/saas/hives/{id}/access` | Hub auth | Access List | `pkg/hub/saas.go:620` |
| `GET` | `/api/saas/grantable-users` | Hub auth | Grantable Users | `pkg/hub/saas.go:621` |
| `POST` | `/api/saas/hives/{id}/access` | Hub auth | Access Add | `pkg/hub/saas.go:622` |
| `DELETE` | `/api/saas/hives/{id}/access/{username}` | Hub auth | Access Remove | `pkg/hub/saas.go:623` |
| `POST` | `/api/saas/hives/{id}/request-access` | Hub auth | Request Access | `pkg/hub/saas.go:624` |
| `GET` | `/api/saas/hives/{id}/requests` | Hub auth | Get Requests | `pkg/hub/saas.go:625` |
| `GET` | `/api/saas/hives/{id}/timeline` | Hub auth | Hive Timeline | `pkg/hub/saas.go:626` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/approve` | Hub auth | Approve Request | `pkg/hub/saas.go:628` |
| `POST` | `/api/saas/hives/{id}/requests/{username}/deny` | Hub auth | Deny Request | `pkg/hub/saas.go:629` |
| `PUT` | `/api/saas/hives/{id}/approve-access/{username}` | Hub auth | Approve Access | `pkg/hub/saas.go:630` |
| `DELETE` | `/api/saas/hives/{id}/deny-access/{username}` | Hub auth | Deny Access | `pkg/hub/saas.go:631` |
| `GET` | `/api/saas/access-status` | Hub handler-specific | Access Status | `pkg/hub/saas.go:632` |
| `POST` | `/api/saas/request-provision` | Hub auth | Request Provision | `pkg/hub/saas.go:640` |
| `PUT` | `/api/saas/approve-provision/{username}` | Hub handler-specific | Approve Provision | `pkg/hub/saas.go:641` |
| `DELETE` | `/api/saas/deny-provision/{username}` | Hub handler-specific | Deny Provision | `pkg/hub/saas.go:642` |
| `GET` | `/api/saas/admin/available-placeholders` | Hub handler-specific | Available Placeholders | `pkg/hub/saas.go:643` |
| `GET` | `/api/saas/admin/users` | Hub handler-specific | Admin Users | `pkg/hub/saas.go:646` |
| `PUT` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Update User | `pkg/hub/saas.go:661` |
| `DELETE` | `/api/saas/admin/users/{username}` | Hub handler-specific | Admin Delete User | `pkg/hub/saas.go:662` |
| `POST` | `/api/saas/admin/impersonate/exit` | Hub handler-specific | Impersonate Exit | `pkg/hub/saas.go:668` |
| `POST` | `/api/saas/admin/impersonate/{username}` | Hub handler-specific | Impersonate Start | `pkg/hub/saas.go:669` |
| `GET` | `/api/saas/impersonation-status` | Hub auth | Impersonation Status | `pkg/hub/saas.go:670` |
| `POST` | `/api/saas/hives/{id}/assign` | Hub auth | Assign Hive | `pkg/hub/saas.go:671` |
| `POST` | `/api/saas/hives/{id}/reset-assignment` | Hub handler-specific | Reset Assignment | `pkg/hub/saas.go:675` |
| `GET` | `/api/saas/cluster-health` | Hub handler-specific | Cluster Health | `pkg/hub/saas.go:676` |
| `POST` | `/api/saas/admin/alert-ack` | Hub handler-specific | Alert Ack | `pkg/hub/saas.go:689` |
| `GET` | `/api/saas/admin/cluster-app-keys` | Hub handler-specific | Get Cluster App Keys | `pkg/hub/saas.go:693` |
| `PUT` | `/api/saas/admin/cluster-app-keys/{clusterID}` | Hub handler-specific | Put Cluster App Key | `pkg/hub/saas.go:694` |
| `POST` | `/api/saas/admin/hub-banner` | Hub handler-specific | Send Hub Banner | `pkg/hub/saas.go:695` |
| `DELETE` | `/api/saas/admin/hub-banner` | Hub handler-specific | Clear Hub Banner | `pkg/hub/saas.go:696` |
| `GET` | `/api/saas/admin/hub-banner` | Hub handler-specific | Get Hub Banner | `pkg/hub/saas.go:697` |
| `POST` | `/api/saas/slack/user/{username}` | Hub auth | Slack Message User | `pkg/hub/saas.go:704` |
| `POST` | `/api/saas/hives/{id}/slack` | Hub auth | Slack Message Hive Owner | `pkg/hub/saas.go:705` |
| `POST` | `/api/saas/admin/slack/broadcast` | Hub handler-specific | Slack Broadcast | `pkg/hub/saas.go:706` |
| `POST` | `/api/saas/admin/journey-snooze` | Hub handler-specific | Journey Snooze | `pkg/hub/saas.go:707` |
| `GET` | `/api/saas/admin/journey-status` | Hub handler-specific | Journey Status | `pkg/hub/saas.go:708` |
| `GET` | `/api/saas/me/country` | Hub auth | My Country Get | `pkg/hub/saas.go:570` |
| `PUT` | `/api/saas/me/country` | Hub auth | My Country Set (write blocked while impersonating) | `pkg/hub/saas.go:571` |
| `POST` | `/api/saas/lite/enroll` | Hub auth | Hive Lite Enroll | `pkg/hub/saas.go:572` |
| `PUT` | `/api/saas/hives/{id}/secondary-app` | Hub auth | Assign Secondary GitHub App | `pkg/hub/saas.go:597` |
| `POST` | `/api/saas/hives/{id}/agents/{agent}/restarts/reset` | Hub auth | Reset Agent Restart Counter | `pkg/hub/saas.go:599` |
| `GET` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch State | `pkg/hub/saas.go:606` |
| `POST` | `/api/saas/upgrade-pause` | Hub admin | Upgrade Kill-Switch Toggle (audit-logged) | `pkg/hub/saas.go:607` |
| `GET` | `/api/saas/whoami` | Hub handler-specific | Session Identity (stable key + profile) | `pkg/hub/saas.go:614` |
| `GET` | `/api/saas/dibs/repos` | Hub handler-specific | Dibs Repo/Owner List (public, short shared cache) | `pkg/hub/saas.go:618` |
| `GET` | `/api/saas/hives/{id}/access-log` | Hub auth | Permission-Change Audit Log | `pkg/hub/saas.go:627` |
| `GET` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Get | `pkg/hub/saas.go:644` |
| `POST` | `/api/saas/admin/scale-settings` | Hub admin | Placeholder Pool Scale Settings Set | `pkg/hub/saas.go:645` |
| `GET` | `/api/saas/admin/user-countries` | Hub admin | User Country Rollup | `pkg/hub/saas.go:651` |
| `GET` | `/api/saas/admin/auth-rollout` | Hub admin | Auth Rollout Readiness Summary | `pkg/hub/saas.go:653` |
| `GET` | `/api/saas/admin/key-generations` | Hub admin | Token-Crypto Key Generations | `pkg/hub/saas.go:659` |
| `POST` | `/api/saas/admin/rotate-master-key` | Hub admin | Rotate Token Master Key (double-call refused) | `pkg/hub/saas.go:660` |
| `GET` | `/api/saas/admin/advisory-diagnostics` | Hub admin | Fleet Advisory/App-State Diagnostics | `pkg/hub/saas.go:686` |
| `POST` | `/api/saas/hives/bulk` | Hub auth | Bulk Hive Action | `pkg/hub/saas_bulk.go:111` |

## Hub server

| Method | Path | Auth | Purpose | Source |
|---|---|---|---|---|
| `GET` | `/login` | Hub handler-specific | Login | `pkg/hub/oauth.go:68` |
| `GET` | `/login/{provider}` | Hub handler-specific | Per-Provider Login (github or OIDC provider) | `pkg/hub/oauth.go:71` |
| `GET` | `/api/auth/callback` | Hub handler-specific | OAuth Callback | `pkg/hub/oauth.go:72` |
| `GET` | `/api/auth/user` | Hub handler-specific | Auth User | `pkg/hub/oauth.go:73` |
| `POST` | `/api/auth/logout` | Hub handler-specific | Logout | `pkg/hub/oauth.go:74` |
| `GET` | `/api/openrouter/connect/start` | Hub auth | Hub Open Router Start | `pkg/hub/openrouter.go:45` |
| `GET` | `/api/openrouter/qr` | Hub auth | Hub Open Router QR | `pkg/hub/openrouter.go:46` |
| `GET` | `/api/openrouter/models` | Hub auth | Hub Open Router Models | `pkg/hub/openrouter.go:47` |
| `GET` | `/api/openrouter/credit` | Hub auth | Hub Open Router Credit | `pkg/hub/openrouter.go:48` |
| `GET` | `/openrouter/callback` | Hub handler-specific | Hub Open Router Callback | `pkg/hub/openrouter.go:49` |
| `GET` | `/dashboard` | Hub handler-specific | Dashboard | `pkg/hub/saas.go:549` |
| `GET` | `/access-denied` | Hub handler-specific | Access Denied | `pkg/hub/saas.go:550` |
| `GET` | `/api/hub/clusters` | Hub auth | List Clusters | `pkg/hub/saas.go:690` |
| `GET` | `/api/hub/image-pulls` | Hub auth | Per-Release Image Pull Series | `pkg/hub/saas.go:556` |
| `GET` | `/api/reach` | Hub admin | PR Reach Report (?pr=NNN or ?recent=K) | `pkg/hub/saas.go:681` |
| `GET` | `/fleet` | Hub handler-specific | My-Hives Fleet Page (static; data via `/api/saas/my-hives`) | `pkg/hub/server.go:1587` |
| `GET` | `/my-hives` | Hub handler-specific | 301 redirect to `/fleet` (query preserved) | `pkg/hub/server.go:1588` |
| `POST` | `/api/heartbeat` | Hub handler-specific | Heartbeat | `pkg/hub/server.go:1549` |
| `POST` | `/api/task-status` | Hub handler-specific | Task Status | `pkg/hub/server.go:1550` |
| `GET` | `/api/registry` | Hub handler-specific | Registry | `pkg/hub/server.go:1551` |
| `GET` | `/api/hub/leaderboard` | Hub handler-specific | Leaderboard | `pkg/hub/server.go:1552` |
| `GET` | `/api/hub/stats` | Hub handler-specific | Stats | `pkg/hub/server.go:1553` |
| `GET` | `/api/fleet-stats` | Hub handler-specific | Fleet Stats | `pkg/hub/server.go:1554` |
| `GET` | `/api/hub/version` | Hub handler-specific | Hub Version | `pkg/hub/server.go:1555` |
| `DELETE` | `/api/hub/registry/{id}` | Hub handler-specific | Registry Delete | `pkg/hub/server.go:1565` |
| `POST` | `/api/contribute/register` | Hub handler-specific | Contribute Proxy | `pkg/hub/server.go:1566` |
| `GET` | `/api/contribute/status` | Hub handler-specific | Contribute Status | `pkg/hub/server.go:1567` |
| `GET` | `/api/contribute/ws` | Hub handler-specific | Contribute WSProxy | `pkg/hub/server.go:1568` |
| `POST` | `/api/github/webhook` | Hub handler-specific | GitHub Webhook | `pkg/hub/server.go:1569` |
| `GET` | `/gh-setup` | Hub handler-specific | GitHub App Setup Router | `pkg/hub/server.go:1570` |
| `GET` | `/learn` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1571` |
| `GET` | `/get-started` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1572` |
| `GET` | `/api/docs` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1573` |
| `GET` | `/api/reading-list` | Hub handler-specific | Reading List | `pkg/hub/server.go:1574` |
| `GET` | `/reading` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1575` |
| `GET` | `/cncf-reference-architecture` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1578` |
| `GET` | `/{$}` | Hub handler-specific | Static HTML page | `pkg/hub/server.go:1595` |
| `GET` | `/og-card.png` | Hub handler-specific | OGCard | `pkg/hub/server.go:1600` |
| `GET` | `/api/hub/delegation-keys` | Public | Delegation-chain verification material (Ed25519 public keys + generation numbers, JWKS-equivalent); deliberately unauthenticated so a tenant or their auditor can verify a chain without a hive credential - see [delegation chain](delegation-chain.md) | `pkg/hub/server.go:1564` |
| `GET` | `/` | Public | Static asset fallback (`http.FileServerFS` over the embedded `static/` tree) for any path no other route claims | `pkg/hub/server.go:1601` |
